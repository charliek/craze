// Package modelcache is the model catalog cache (plan 030 §3.14): the last
// model catalog a detached host saw each ACP provider install, one small JSON
// file per provider in the cache tree's catalogs directory
// (rundir.CatalogDir), so the session list's /model can offer a provider's
// models before any session of it has started. An ACP agent names its models
// only in its answer to session/new and in the updates after it (discovery
// data.md §5); native's are its model table, which needs no cache.
//
// A file is one object, at most MaxBytes:
//
//	{"version":1,"observedAt":"2026-09-30T12:00:00Z","provider":"cursor",
//	 "models":[{"id":"grok-4.6","name":"Grok 4.6"}, …]}
//
// Two hosts of one provider can install catalogs at the same moment, and an
// atomic replace alone would let the older observation, finishing last,
// replace the newer one (R2-12). So a writer takes <provider>.lock — an
// flock, one per provider, beside the file — reads the file's observedAt, and
// replaces the file only when its own observation is newer, the comparison
// and the replace both under the lock (Write). The replace is
// internal/atomicfile's rename, so a reader, which takes no lock, reads one
// whole file or the other.
//
// A file this package did not write — one that does not parse, another
// version's, another provider's, one with no models or a model with no id,
// text that is not one line of UTF-8, larger than MaxBytes, not a regular
// file — is ignored as though there were none (Read's error), and the next
// write replaces it.
package modelcache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charliek/craze/internal/atomicfile"
)

// Version is the file's schema version. A file of any other is ignored.
const Version = 1

// MaxBytes bounds a catalog file (plan 030 §3.14): a catalog whose file would
// be larger is not written, and a larger file is ignored. A real catalog is a
// few kilobytes — ids and names, nothing more.
const MaxBytes = 256 << 10

// maxProviderLen bounds a provider id: it names two files.
const maxProviderLen = 64

// lockWait bounds how long a writer waits for another's lock: a writer holds
// it for one small read and one small write, so a lock busy for this long is
// a writer stuck, and this one gives up (ErrLockBusy's error) rather than
// wait on it for ever. A variable only so a test that holds a writer inside
// its lock on purpose can lengthen it.
var lockWait = 2 * time.Second

// replaceHook, when a test sets it, is called by Write under the lock, after
// the comparison and before the replace: the place a test holds a writer to
// force the schedule R2-12 is about. nil in production.
var replaceHook func(Catalog)

// Errors Read and Write answer. Read's are all "ignore this file".
var (
	// ErrCorrupt is a file this package did not write, or a catalog it
	// would not write: see the package comment.
	ErrCorrupt = errors.New("modelcache: not a model catalog craze writes")
	// ErrTooLarge is a file, or a catalog to write, larger than MaxBytes.
	ErrTooLarge = errors.New("modelcache: the catalog is larger than 256 KiB")
)

// Model is one model of a catalog: the id a session is started on (--model),
// and the name the agent shows for it ("" is the id).
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Valid says m can be written: an id, and both texts one line of UTF-8 with
// no control character — a menu row each, and never an escape sequence.
func (m Model) Valid() bool {
	return m.ID != "" && oneLine(m.ID) && oneLine(m.Name)
}

// Catalog is one provider's catalog as a host observed it at ObservedAt.
type Catalog struct {
	Version    int       `json:"version"`
	ObservedAt time.Time `json:"observedAt"`
	Provider   string    `json:"provider"`
	Models     []Model   `json:"models"`
}

// ValidProvider says id can name a catalog: one to 64 of a–z, 0–9, '-' and
// '_', the first a letter or a digit — every provider id craze has, and
// never a path.
func ValidProvider(id string) bool {
	if id == "" || len(id) > maxProviderLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '-' || c == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// Read is provider's catalog in dir, the directory rundir.CatalogDir
// validated. Any error means there is none to offer: the file is not there
// (fs.ErrNotExist), or is not one this package wrote (ErrCorrupt,
// ErrTooLarge) — which the caller ignores, as the next Write does.
func Read(dir, provider string) (Catalog, error) {
	if !ValidProvider(provider) {
		return Catalog{}, fmt.Errorf("%w: %q is not a provider id", ErrCorrupt, provider)
	}
	// Never through a link, and never waiting on a FIFO someone left there:
	// the directory is this user's own, and still only a regular file is a
	// catalog.
	f, err := os.OpenFile(catalogPath(dir, provider), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Catalog{}, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	switch {
	case err != nil:
		return Catalog{}, err
	case !st.Mode().IsRegular():
		return Catalog{}, fmt.Errorf("%w: not a regular file", ErrCorrupt)
	case st.Size() > MaxBytes:
		return Catalog{}, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return Catalog{}, err
	}
	if len(data) > MaxBytes {
		// Grown since the stat.
		return Catalog{}, ErrTooLarge
	}
	return decode(data, provider)
}

// decode is data as provider's catalog: exactly one object, holding what
// check asks of one.
func decode(data []byte, provider string) (Catalog, error) {
	var c Catalog
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&c); err != nil {
		return Catalog{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Catalog{}, fmt.Errorf("%w: more than one object", ErrCorrupt)
	}
	if err := c.check(provider); err != nil {
		return Catalog{}, err
	}
	return c, nil
}

// check is c as a catalog of provider this package writes and reads: this
// version, that provider, an observation time, and at least one model, each
// Valid.
func (c Catalog) check(provider string) error {
	switch {
	case c.Version != Version:
		return fmt.Errorf("%w: version %d", ErrCorrupt, c.Version)
	case !ValidProvider(c.Provider) || c.Provider != provider:
		return fmt.Errorf("%w: provider %q", ErrCorrupt, c.Provider)
	case c.ObservedAt.IsZero():
		return fmt.Errorf("%w: no observation time", ErrCorrupt)
	case len(c.Models) == 0:
		return fmt.Errorf("%w: no models", ErrCorrupt)
	}
	for _, m := range c.Models {
		if !m.Valid() {
			return fmt.Errorf("%w: a model with no id, or text that is not one line", ErrCorrupt)
		}
	}
	return nil
}

// Write records c as its provider's catalog in dir, the directory
// rundir.CatalogDir validated — unless the file there already holds an
// observation at least as new as c's, which it leaves alone (written false, a
// nil error). Under <provider>.lock, held from before the file is read until
// after it is replaced, so of two writers the newer observation is the one
// left, whichever finishes last (R2-12). A file there that Read would ignore
// is replaced. c's Version is set; a catalog check refuses, or one larger
// than MaxBytes, is an error and writes nothing.
func Write(dir string, c Catalog) (written bool, err error) {
	c.Version = Version
	// The wall clock alone: a monotonic reading would compare against a time
	// read back from a file, which has none, by another rule.
	c.ObservedAt = c.ObservedAt.Round(0).UTC()
	if err := c.check(c.Provider); err != nil {
		return false, err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return false, err
	}
	data = append(data, '\n')
	if len(data) > MaxBytes {
		return false, ErrTooLarge
	}
	unlock, err := atomicfile.LockWithin(filepath.Join(dir, c.Provider+".lock"), lockWait)
	if err != nil {
		return false, fmt.Errorf("modelcache: the %s catalog's lock: %w", c.Provider, err)
	}
	defer unlock()
	if old, err := Read(dir, c.Provider); err == nil && !c.ObservedAt.After(old.ObservedAt) {
		return false, nil
	}
	if replaceHook != nil {
		replaceHook(c)
	}
	if err := atomicfile.Write(catalogPath(dir, c.Provider), data, 0o600); err != nil {
		return false, fmt.Errorf("modelcache: write the %s catalog: %w", c.Provider, err)
	}
	return true, nil
}

// catalogPath is provider's catalog file in dir.
func catalogPath(dir, provider string) string { return filepath.Join(dir, provider+".json") }

// oneLine says s is valid UTF-8 with no control character in it.
func oneLine(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
