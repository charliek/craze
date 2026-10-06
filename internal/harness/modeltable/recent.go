package modeltable

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
)

// Model memory (plan 031 §3.4, owner decision Q1): the models a session's
// /model picked, newest first, each with the effort last used on it, so that
// the next session starts where the last one left off rather than where
// nothing remembered would put it (StartPick). It is one small file beside
// the two model files, recent.json, which nothing but a model switch or an
// effort change made in a
// running session writes: not --model, not `craze prompt`, not a resume, not a
// sub-agent, and not plan 030's session list, each of which applies to one
// start only.
//
// The file names each model by alias and by identity (provider, wire model),
// both taken from the table of the session that made the switch (r2-5) —
// Remember never loads the model files to fill them in — so a catalog that
// renames a model keeps the memory, and one that re-points an alias at
// another model does not carry it over (Table.Recent). Reading it never fails
// anything: a missing, unreadable, corrupt or newer file is no memory at all.
// Nothing in it is ever pruned; an entry no model matches is skipped by every
// reader and dropped only when it falls off the end.

// RecentFile is the model memory's file, in the native directory beside
// ProvidersFile and ModelsFile.
const RecentFile = "recent.json"

// RecentCap is the most models recent.json keeps: opencode's own bound for
// its recent list (plan 031 §2, references).
const RecentCap = 10

// RecentLockWait bounds how long Remember waits for another writer of
// recent.json to finish (atomicfile.LockWithin): a switch in another session,
// which holds the lock for one small read and write. A session switching
// models waits at most this long before it gives up saving the choice — the
// switch itself has already happened.
const RecentLockWait = 2 * time.Second

// RecentMaxBytes is the most of recent.json craze will read: ten entries are a
// few hundred bytes, so 64 KiB is far past any file craze wrote. A larger file
// is not one (plan 031 X33): ReadRecent takes it as no memory and Remember
// refuses to overwrite it, rather than spend unbounded time and memory on it.
const RecentMaxBytes = 64 << 10

// recentPerm is recent.json's mode: it names nothing secret, but it is a
// record of what the owner has been using, so it is kept as private as the
// directory it lives in.
const recentPerm os.FileMode = 0o600

// recentLockName is the lock every writer of recent.json takes, beside it.
const recentLockName = RecentFile + ".lock"

// ErrRecentNewer is Remember's error for a recent.json whose version is newer
// than Version: a later craze wrote it, and this one leaves it as it is rather
// than replace it with a shape that craze may not read back.
var ErrRecentNewer = errors.New("modeltable: recent.json was written by a newer craze")

// RecentEntry is one remembered model: the alias a session switched to, its
// identity in that session's table, and the effort the session was then at.
// On disk it is one element of recent.json's "recent" list.
type RecentEntry struct {
	// Alias is the model's alias as the switching session's table spelled
	// it. On disk it is "model".
	Alias string `json:"model"`
	// Provider and WireModel are the model's identity in that table: how a
	// reader finds it again when the alias no longer names it (Table.Recent).
	Provider  string `json:"provider"`
	WireModel string `json:"wire_model"`
	// Effort is the effort the session was at after the change, as the
	// harness confirmed it; "" for a model with no effort control.
	Effort string `json:"effort"`
	// At is when the choice was made, for a person reading the file; no
	// reader orders by it (the list's order is the order).
	At time.Time `json:"at"`
}

// recentDoc is recent.json. Entries are decoded one at a time (readRecent),
// so one that does not decode is skipped rather than costing the others.
type recentDoc struct {
	Version int           `json:"version"`
	Recent  []RecentEntry `json:"recent"`
}

// ReadRecent is dir's model memory, newest first, one entry per alias. It
// never fails: a missing file, one that cannot be read, one that is not a
// recent.json, and one a newer craze wrote are each no memory at all, and an
// entry that does not decode or names no model is skipped. The entries are as
// the file holds them; Table.Recent says which of them a table still has.
func ReadRecent(dir string) []RecentEntry {
	if dir == "" {
		return nil
	}
	entries, _ := readRecent(filepath.Join(dir, RecentFile))
	return entries
}

// readRecent is recent.json at path, and why it could not be kept when a
// writer must leave it alone: an error for a file that exists but cannot be
// read (replacing it would lose what nobody could see) and ErrRecentNewer for
// a newer version. A missing file is (nil, nil). So is one that is not a
// recent.json at all — not JSON, no version, an older or a nonsensical one:
// there is nothing in it any craze can read back, so the next write replaces
// it.
func readRecent(path string) ([]RecentEntry, error) {
	b, err := readRecentBytes(path)
	if err != nil || b == nil {
		return nil, err
	}
	var head struct {
		Version int               `json:"version"`
		Recent  []json.RawMessage `json:"recent"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return nil, nil
	}
	switch {
	case head.Version > Version:
		return nil, fmt.Errorf("%w (version %d; this craze reads version %d), so it is left as it is: %s",
			ErrRecentNewer, head.Version, Version, path)
	case head.Version != Version:
		return nil, nil
	}
	var out []RecentEntry
	seen := make(map[string]bool, len(head.Recent))
	for _, raw := range head.Recent {
		var e RecentEntry
		// A file edited by hand can name a model twice: the newest mention,
		// the first, is the one that counts.
		if json.Unmarshal(raw, &e) != nil || strings.TrimSpace(e.Alias) == "" || seen[e.Alias] {
			continue
		}
		seen[e.Alias] = true
		out = append(out, e)
	}
	return out, nil
}

// readRecentBytes is recent.json's bytes, or nil for a missing file. The read
// is bounded and does not wait on a FIFO: the file is opened O_NONBLOCK (so a
// FIFO opens at once, and a symlink to one is no different) and refused unless
// the opened file is a regular one, and at most RecentMaxBytes are read. That
// bounds the bytes, not the time: a stalled filesystem can still delay a
// regular read. A file
// that is neither regular nor within the cap is an error, which Remember
// treats like an unreadable file (left as it is) and ReadRecent as no memory.
func readRecentBytes(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("modeltable: %w", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return nil, fmt.Errorf("modeltable: %w", err)
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("modeltable: %s is not a regular file, so it is left as it is", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, RecentMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("modeltable: %w", err)
	}
	if len(b) > RecentMaxBytes {
		return nil, fmt.Errorf("modeltable: %s is larger than %d bytes, so it is left as it is", path, RecentMaxBytes)
	}
	if b == nil {
		b = []byte{}
	}
	return b, nil
}

// Remember records e as the newest entry of dir's model memory: any earlier
// entry for its alias goes, the list keeps at most RecentCap entries, and e's
// At is now, in UTC to the second. The caller builds e from the table of the
// session that made the switch (plan 031 r2-5) — the alias it switched to,
// that alias's provider and wire model, and the effort the harness confirmed
// after the change — and Remember takes it as it is: it never loads the model
// files.
//
// Two sessions switching at once lose neither choice: the read, the change
// and the write happen under recent.json.lock, taken with a bound
// (atomicfile.LockWithin, RecentLockWait) so a switch never waits on another
// craze for longer. The directory is created 0700 before the lock is opened,
// and the file is written whole, atomically, at 0600.
//
// It returns an error, and changes nothing, when dir is "", when the lock is
// not had within the bound (unwrapping to atomicfile.ErrLockBusy), when an
// existing recent.json cannot be read, when a newer craze wrote it
// (ErrRecentNewer), or when the write fails. A recent.json that is not one —
// not JSON, or no version this craze knows — is replaced. None of this is a
// failed switch: the caller says it once and carries on.
func Remember(dir string, e RecentEntry, now time.Time) error {
	return remember(dir, e, now, recentIO{lock: atomicfile.LockWithin, wait: RecentLockWait})
}

// recentIO is how remember takes the lock, and a seam: tests hand in a lock
// that says when it is tried, a bound of their own, and afterRead, which runs
// inside the locked section between the read and the write.
type recentIO struct {
	lock      func(path string, d time.Duration) (unlock func(), err error)
	wait      time.Duration
	afterRead func()
}

func remember(dir string, e RecentEntry, now time.Time, io recentIO) error {
	if dir == "" {
		// paths.NativeDir is "" with no home directory; joining "" would
		// write recent.json into the working directory.
		return errors.New("modeltable: no directory to remember the model in")
	}
	if strings.TrimSpace(e.Alias) == "" {
		return errors.New("modeltable: a remembered model needs an alias")
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("modeltable: %w", err)
	}
	path := filepath.Join(dir, RecentFile)
	unlock, err := io.lock(filepath.Join(dir, recentLockName), io.wait)
	if errors.Is(err, atomicfile.ErrLockBusy) {
		return fmt.Errorf("modeltable: %s: another craze held its lock for %v: %w", path, io.wait, err)
	}
	if err != nil {
		return fmt.Errorf("modeltable: locking %s: %w", path, err)
	}
	defer unlock()
	read, err := readRecent(path)
	if err != nil {
		return err
	}
	if io.afterRead != nil {
		io.afterRead()
	}
	e.At = now.UTC().Truncate(time.Second)
	list := append([]RecentEntry{e}, slices.DeleteFunc(read, func(o RecentEntry) bool { return o.Alias == e.Alias })...)
	if len(list) > RecentCap {
		list = list[:RecentCap]
	}
	b, err := json.MarshalIndent(recentDoc{Version: Version, Recent: list}, "", "  ")
	if err != nil {
		return fmt.Errorf("modeltable: %w", err)
	}
	if err := atomicfile.Write(path, append(b, '\n'), recentPerm); err != nil {
		return fmt.Errorf("modeltable: %w", err)
	}
	return nil
}

// Recent is the model memory as t has it: each entry that still names one of
// t's models, under the alias that model has in t, newest first, one per
// alias. An entry names a model by its alias when that alias is still a model
// with the entry's identity (provider, wire model); otherwise by its identity
// alone, under the first alias in sorted order that has it — so a catalog
// release that renames a model keeps the memory; and otherwise it is skipped
// (an alias re-pointed at another model is not the model that was picked).
// Nothing is pruned: a skipped entry stays in the file. Whether a model's
// provider has a key is not judged here; every reader that needs a funded
// model — StartModel, RememberedEffort's callers — judges that itself.
func (t *Table) Recent(recent []RecentEntry) []RecentEntry {
	var out []RecentEntry
	seen := make(map[string]bool, len(recent))
	for _, e := range recent {
		alias, ok := t.recentAlias(e)
		if !ok || seen[alias] {
			continue
		}
		seen[alias] = true
		e.Alias = alias
		out = append(out, e)
	}
	return out
}

// recentAlias is the alias e names in t (Recent's rule), and whether it names
// one.
func (t *Table) recentAlias(e RecentEntry) (string, bool) {
	id := identity{e.Provider, e.WireModel}
	same := func(alias string) bool {
		m, ok := t.Models[alias]
		return ok && identity{m.Provider, m.WireModel} == id
	}
	if same(e.Alias) {
		return e.Alias, true
	}
	for _, alias := range t.Aliases() {
		if same(alias) {
			return alias, true
		}
	}
	return "", false
}

// StartModel is the model and effort a new session with no --model starts on
// (plan 031 §3.5, plan 038 §2.3); StartPick says which step chose it.
func (t *Table) StartModel(recent []RecentEntry, getenv func(string) string) (alias, effort string, err error) {
	p, err := t.StartPick(recent, getenv)
	return p.Alias, p.Effort, err
}

// Start is where StartPick starts a new session: a model, its effort, and
// whether it is the last step's fallback.
type Start struct {
	Alias, Effort string
	// Fallback says the model is the first funded alias in sorted order, the
	// last step: nothing remembered resolves, no pin is funded, and no
	// provider in the order has a start that resolves. The caller says the
	// session did not start where it would have.
	Fallback bool
}

// StartPick is where a new session with no --model starts (plan 031 §3.5,
// plan 038 §2.3), the first of:
//
//  1. the newest entry of recent (through Recent) whose model resolves —
//     its provider has a usable key, judged exactly as Resolve judges it with
//     getenv — at the effort remembered with it when the model still offers
//     it, and otherwise at its default_effort;
//  2. the user's pin, DefaultModel, at its default_effort, unless its
//     provider has no key. A pin that fails for any other reason is still
//     the answer, left for the caller's open to report, so the user sees it
//     (plan 018 §3.8);
//  3. for each provider in StartOrder, the first of its start models that
//     resolves, at its default_effort — the ChatGPT plan's when the account
//     lists it and the sign-in funds it (plan 034 Q4): an owner starts on
//     the chosen model of the first provider they fund, not on whichever
//     sorts first;
//  4. the first alias in sorted order that resolves, at its default_effort,
//     with Fallback set: one unfunded choice must not lock the owner out of
//     the others.
//
// With none of them — a fresh machine with the shipped catalog and no key —
// the error is ErrNothingFunded, which also unwraps to the ErrNoAPIKey of the
// first model it tried that has no key: the pin's, else the first start
// model's, else the first alias's. A nil getenv is os.Getenv.
func (t *Table) StartPick(recent []RecentEntry, getenv func(string) string) (Start, error) {
	for _, e := range t.Recent(recent) {
		if _, err := t.Resolve(e.Alias, getenv); err == nil {
			return Start{Alias: e.Alias, Effort: cmp.Or(t.offeredEffort(e.Alias, e.Effort), t.Models[e.Alias].DefaultEffort)}, nil
		}
	}
	var unfunded error // the first ErrNoAPIKey met, which ErrNothingFunded wraps
	try := func(alias string) bool {
		_, err := t.Resolve(alias, getenv)
		if unfunded == nil && errors.Is(err, ErrNoAPIKey) {
			unfunded = err
		}
		return err == nil
	}
	if pin := t.DefaultModel; pin != "" {
		_, err := t.Resolve(pin, getenv)
		if !errors.Is(err, ErrNoAPIKey) {
			return Start{Alias: pin, Effort: t.Models[pin].DefaultEffort}, nil
		}
		unfunded = err
	}
	for _, alias := range t.StartAliases() {
		if try(alias) {
			return Start{Alias: alias, Effort: t.Models[alias].DefaultEffort}, nil
		}
	}
	for _, alias := range t.Aliases() {
		if try(alias) {
			return Start{Alias: alias, Effort: t.Models[alias].DefaultEffort, Fallback: true}, nil
		}
	}
	if unfunded == nil {
		return Start{}, ErrNothingFunded
	}
	return Start{}, fmt.Errorf("%w (%w)", ErrNothingFunded, unfunded)
}

// StartOrder is the order a new session with nothing remembered and no funded
// pin tries the providers in (plan 038 §2.1, §2.4): the user's provider_order
// when models.toml writes one, else the catalog's; nil for a table with
// neither.
func (t *Table) StartOrder() []string {
	if t.ProviderOrder != nil {
		return slices.Clone(t.ProviderOrder)
	}
	return slices.Clone(t.catalogOrder)
}

// StartAliases is every provider's start model in StartOrder, each
// provider's in its own order of preference (plan 038 §2.2): what
// StartPick's third step tries. A provider listed twice counts once, and a
// start alias whose model the user's files moved to another provider is
// left out — it is no longer that provider's model. Whether a model resolves
// is not judged here.
func (t *Table) StartAliases() []string {
	var out []string
	var seen []string
	for _, id := range t.StartOrder() {
		if slices.Contains(seen, id) {
			continue
		}
		seen = append(seen, id)
		for _, alias := range t.starts[id] {
			if m, ok := t.Models[alias]; ok && m.Provider == id && !slices.Contains(out, alias) {
				out = append(out, alias)
			}
		}
	}
	return out
}

// offeredEffort is effort when alias's model still offers it, else "": what
// StartModel and RememberedEffort take from a remembered effort.
func (t *Table) offeredEffort(alias, effort string) string {
	if effort != "" && slices.Contains(t.Models[alias].Efforts, effort) {
		return effort
	}
	return ""
}

// RememberedEffort is the effort recent remembers for alias, through Recent —
// so a renamed model's memory is found under its new alias — when alias's
// model still offers it; "" otherwise, which leaves the session at the model's
// default_effort. It is for a new session started on an explicit model (--model,
// or plan 030's list): the flag picks the model, and the memory still picks
// its effort (plan 031 P6). A resume never takes it — the transcript's effort
// is the conversation's own.
func (t *Table) RememberedEffort(recent []RecentEntry, alias string) string {
	for _, e := range t.Recent(recent) {
		if e.Alias == alias {
			return t.offeredEffort(alias, e.Effort)
		}
	}
	return ""
}
