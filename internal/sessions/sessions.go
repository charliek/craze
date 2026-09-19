// Package sessions is the on-disk index of agent sessions craze has started
// or loaded, ~/.craze/sessions.jsonl (internal/paths.SessionsPath). It backs
// `--continue`, `--resume` and `/rename`: one JSON object per line, newest
// activity first when read back.
//
// sessions deliberately does not import internal/agent: an unknown provider
// id is kept in the file (a row from a build that knew more providers) but
// never offered by Latest/Recent, and the caller supplies what "known" means
// through Store.KnownProvider rather than sessions reaching into the
// provider registry itself.
package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/paths"
)

// maxRows caps the index; a write that would overflow it drops the oldest
// rows by UpdatedAt (ties broken by file order, earlier position dropped
// first).
const maxRows = 500

// indexLockSuffix names the sibling file Upsert flocks, mirroring config.go's
// convention: a lock on the index itself would be a lock on an inode the
// next rewrite replaces.
const indexLockSuffix = ".lock"

// TitleKind classifies the Title carried by a Row passed to Upsert, because
// the four merge rules in play (see Upsert's doc) depend on where a title
// came from, not just its text. It rides on Row itself -- rather than a
// second Upsert argument -- so Store keeps the single-method shape
// (Upsert(Row) error) that internal/tui's Config.SessionIndex interface
// wants: a Row is both the record Latest/Recent return (TitleKind is always
// TitleKindNone there; it is never persisted) and the merge instruction
// Upsert takes.
type TitleKind int

const (
	// TitleKindNone carries no title change: a "touch" that only bumps
	// UpdatedAt (e.g. on EventDone, or when a loaded session's replay
	// ends). Row.Title should be "" for this kind; a non-empty Title is
	// ignored defensively rather than applied.
	TitleKindNone TitleKind = iota
	// TitleKindFallback is the first line of the user's first prompt. It
	// only fills a Title that is currently empty -- it never overwrites an
	// existing title, pinned or not.
	TitleKindFallback
	// TitleKindAgent is the provider's own session_info_update title. It
	// overwrites any non-pinned title, but a /rename pin blocks it.
	TitleKindAgent
	// TitleKindUser is a /rename. It always overwrites and pins the row so
	// that no later TitleKindAgent title can override it again.
	TitleKindUser
)

// Row is one line of the index. Timestamps are RFC3339Nano, UTC.
type Row struct {
	SessionID string    `json:"sessionId"`
	Provider  string    `json:"provider"`
	CWD       string    `json:"cwd"`
	Title     string    `json:"title"`
	Pinned    bool      `json:"pinned"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// TitleKind is only meaningful as input to Upsert; see the type's doc.
	// It is never read from or written to the file.
	TitleKind TitleKind `json:"-"`
}

// record is a decoded line plus whatever the store does not model: unknown
// fields (round-tripped so a newer craze's extra keys survive an older
// craze's rewrite, exactly as config.go treats unknown TOML keys) and the
// line's position in the file, which breaks UpdatedAt ties.
type record struct {
	Row   Row
	extra map[string]any
	pos   int
}

type key struct {
	provider  string
	sessionID string
}

func (r record) key() key { return key{provider: r.Row.Provider, sessionID: r.Row.SessionID} }

// Store is the session index. The zero value is ready to use: like
// config.go, every method recomputes paths.SessionsPath() itself, so a test
// that changes HOME or CRAZE_HOME between calls (or the frame runner, which
// swaps both for the length of a run) always reads and writes the current
// file.
type Store struct {
	// KnownProvider reports whether a provider id is one this build knows
	// about. Latest and Recent skip rows for a provider this returns false
	// for, but never remove them from the file -- a row from a build that
	// knew more providers is kept, just not offered. nil means every
	// provider is known (no filtering). This is a plain function value,
	// not an internal/agent type, so this package's dependency graph stays
	// one-directional: internal/agent (and internal/tui, internal/cli) may
	// import internal/sessions, never the reverse.
	KnownProvider func(string) bool
}

// Upsert creates or updates the row for (in.Provider, in.SessionID),
// bumping UpdatedAt and applying in's title according to in.TitleKind:
//
//   - an empty in.Title never overwrites the existing title (or sets
//     Pinned), regardless of kind -- this is the universal guard checked
//     before any kind-specific rule;
//   - TitleKindFallback only fills a Title that is currently empty;
//   - TitleKindAgent overwrites unless the row is already Pinned;
//   - TitleKindUser always overwrites and sets Pinned.
//
// CreatedAt is set once, when the row is first created. The whole operation
// -- read, merge by key, rewrite -- runs under atomicfile.Lock; unlike
// config.go's SaveTheme/SaveProvider, a lock failure here is a returned
// error rather than a swallowed no-op, because sessions.jsonl is an append
// log two crazes in one workspace can both be writing to, and skipping the
// lock risks losing a row rather than losing a cosmetic preference.
//
// A write that overflows the 500-row cap drops the oldest rows by
// UpdatedAt, ties broken by file order (the earlier row is dropped first).
func (s *Store) Upsert(in Row) error {
	if strings.TrimSpace(in.SessionID) == "" || strings.TrimSpace(in.Provider) == "" || strings.TrimSpace(in.CWD) == "" {
		return errors.New("sessions: upsert requires a non-empty sessionId, provider and cwd")
	}
	path := paths.SessionsPath()
	if path == "" {
		return errors.New("craze: no home directory to save the session index in")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := atomicfile.Lock(path + indexLockSuffix)
	if err != nil {
		return fmt.Errorf("craze: not saving the session: %w", err)
	}
	defer unlock()

	records, err := readRecords(path)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	k := key{provider: in.Provider, sessionID: in.SessionID}
	found := false
	for i := range records {
		if records[i].key() != k {
			continue
		}
		records[i].Row.CWD = in.CWD
		records[i].Row.UpdatedAt = now
		applyTitle(&records[i].Row, in)
		found = true
		break
	}
	if !found {
		rec := record{Row: Row{
			SessionID: in.SessionID,
			Provider:  in.Provider,
			CWD:       in.CWD,
			CreatedAt: now,
			UpdatedAt: now,
		}}
		applyTitle(&rec.Row, in)
		records = append(records, rec)
	}

	records = evictOverflow(records)

	return writeRecords(path, records)
}

// applyTitle folds in's title into row according to in.TitleKind, per
// Upsert's doc.
func applyTitle(row *Row, in Row) {
	if in.Title == "" {
		return
	}
	switch in.TitleKind {
	case TitleKindUser:
		row.Title = in.Title
		row.Pinned = true
	case TitleKindAgent:
		if !row.Pinned {
			row.Title = in.Title
		}
	case TitleKindFallback:
		if row.Title == "" {
			row.Title = in.Title
		}
	case TitleKindNone:
		// A non-empty Title with TitleKindNone should not happen (a pure
		// "touch" carries no title); do nothing rather than guess.
	}
}

// evictOverflow drops the oldest rows (by UpdatedAt, ties broken by file
// order -- the earlier position loses the tie) once records exceeds
// maxRows, otherwise it returns records unchanged. The relative order of
// the rows that survive is preserved.
func evictOverflow(records []record) []record {
	if len(records) <= maxRows {
		return records
	}
	order := make([]int, len(records))
	for i := range records {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		ra, rb := records[order[a]], records[order[b]]
		if !ra.Row.UpdatedAt.Equal(rb.Row.UpdatedAt) {
			return ra.Row.UpdatedAt.Before(rb.Row.UpdatedAt)
		}
		return order[a] < order[b]
	})
	drop := len(records) - maxRows
	dropped := make(map[int]bool, drop)
	for _, i := range order[:drop] {
		dropped[i] = true
	}
	out := make([]record, 0, maxRows)
	for i, rec := range records {
		if !dropped[i] {
			out = append(out, rec)
		}
	}
	return out
}

// Latest is the newest row matching cwd (exact string equality, no symlink
// resolution) and provider ("" means any provider), or ok=false if there is
// none.
func (s *Store) Latest(cwd, provider string) (Row, bool, error) {
	rows, err := s.Recent(cwd, provider, 1)
	if err != nil {
		return Row{}, false, err
	}
	if len(rows) == 0 {
		return Row{}, false, nil
	}
	return rows[0], true, nil
}

// Recent returns up to n rows matching cwd and provider, newest first
// (UpdatedAt descending, ties broken by file order -- the later row in the
// file wins the tie and sorts first). n <= 0 means no cap. A provider this
// Store's KnownProvider reports as unknown is filtered out here, but never
// removed from the file. No home directory reads as zero rows and no error.
func (s *Store) Recent(cwd, provider string, n int) ([]Row, error) {
	path := paths.SessionsPath()
	if path == "" {
		return nil, nil
	}
	records, err := readRecords(path)
	if err != nil {
		return nil, err
	}

	filtered := make([]record, 0, len(records))
	for _, rec := range records {
		if rec.Row.CWD != cwd {
			continue
		}
		if provider != "" && rec.Row.Provider != provider {
			continue
		}
		if s.KnownProvider != nil && !s.KnownProvider(rec.Row.Provider) {
			continue
		}
		filtered = append(filtered, rec)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		ui, uj := filtered[i].Row.UpdatedAt, filtered[j].Row.UpdatedAt
		if !ui.Equal(uj) {
			return ui.After(uj)
		}
		return filtered[i].pos > filtered[j].pos
	})
	if n > 0 && len(filtered) > n {
		filtered = filtered[:n]
	}
	rows := make([]Row, len(filtered))
	for i, rec := range filtered {
		rows[i] = rec.Row
	}
	return rows, nil
}

// readRecords decodes the index a line at a time. A blank line is skipped.
// A missing file reads as zero rows, like config.go's readConfigAt. Any
// other decode failure -- the line is not a JSON object, or lacks a
// non-empty sessionId/provider/cwd, or has an unparsable updatedAt -- fails
// the whole read with an error naming the file and the 1-based line number.
// Duplicate keys: the last occurrence wins, keeping its file position for
// tie-breaks. Reads take no lock: rename atomicity rules out a torn read,
// and a stale one is harmless; a reader never rewrites the file.
func readRecords(path string) ([]record, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	records := make([]record, 0, len(lines))
	pos := 0
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		rec, err := decodeLine(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, i+1, err)
		}
		rec.pos = pos
		pos++
		records = append(records, rec)
	}
	return dedupeLastWins(records), nil
}

// dedupeLastWins keeps, for each (provider, sessionId) key, only the last
// occurrence in the file (a torn earlier run), in the key's first-seen
// order.
func dedupeLastWins(records []record) []record {
	lastIndex := make(map[key]int, len(records))
	var order []key
	for i, rec := range records {
		k := rec.key()
		if _, ok := lastIndex[k]; !ok {
			order = append(order, k)
		}
		lastIndex[k] = i
	}
	out := make([]record, 0, len(order))
	for _, k := range order {
		out = append(out, records[lastIndex[k]])
	}
	return out
}

// decodeLine decodes one non-blank line into a record, validating the
// fields §3.2 requires and preserving every field it does not recognise.
func decodeLine(line string) (record, error) {
	// UseNumber keeps an unknown numeric field as its literal text rather
	// than a float64, so a large integer another build wrote round-trips
	// through this older one without losing precision.
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return record{}, fmt.Errorf("not a JSON object: %w", err)
	}
	if raw == nil {
		return record{}, errors.New("not a JSON object: null")
	}
	// json.Unmarshal rejects trailing data; a Decoder does not, so say so
	// explicitly rather than silently accepting two objects on one line.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return record{}, errors.New("not a JSON object: trailing data after the object")
	}

	sessionID, _ := raw["sessionId"].(string)
	if strings.TrimSpace(sessionID) == "" {
		return record{}, errors.New("missing a non-empty sessionId")
	}
	provider, _ := raw["provider"].(string)
	if strings.TrimSpace(provider) == "" {
		return record{}, errors.New("missing a non-empty provider")
	}
	cwd, _ := raw["cwd"].(string)
	if strings.TrimSpace(cwd) == "" {
		return record{}, errors.New("missing a non-empty cwd")
	}

	updatedRaw, _ := raw["updatedAt"].(string)
	updatedAt, err := time.Parse(time.RFC3339Nano, updatedRaw)
	if err != nil {
		return record{}, fmt.Errorf("unparsable updatedAt %q: %w", updatedRaw, err)
	}

	// createdAt is not in the validation list: a row this package itself
	// never wrote a valid one for is not expected, but reading defensively
	// (zero time on anything unparsable) keeps a read from failing over a
	// field that is not part of the read contract.
	var createdAt time.Time
	if createdRaw, ok := raw["createdAt"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, createdRaw); err == nil {
			createdAt = t
		}
	}

	title, _ := raw["title"].(string)
	pinned, _ := raw["pinned"].(bool)

	extra := make(map[string]any, len(raw))
	for k, v := range raw {
		extra[k] = v
	}
	for _, k := range []string{"sessionId", "provider", "cwd", "title", "pinned", "createdAt", "updatedAt"} {
		delete(extra, k)
	}

	return record{
		Row: Row{
			SessionID: sessionID,
			Provider:  provider,
			CWD:       cwd,
			Title:     title,
			Pinned:    pinned,
			CreatedAt: createdAt,
			UpdatedAt: updatedAt,
		},
		extra: extra,
	}, nil
}

// encodeLine is decodeLine's inverse: known fields plus whatever unknown
// ones travelled in on extra.
func encodeLine(rec record) ([]byte, error) {
	out := make(map[string]any, len(rec.extra)+7)
	for k, v := range rec.extra {
		out[k] = v
	}
	out["sessionId"] = rec.Row.SessionID
	out["provider"] = rec.Row.Provider
	out["cwd"] = rec.Row.CWD
	out["title"] = rec.Row.Title
	out["pinned"] = rec.Row.Pinned
	out["createdAt"] = rec.Row.CreatedAt.UTC().Format(time.RFC3339Nano)
	out["updatedAt"] = rec.Row.UpdatedAt.UTC().Format(time.RFC3339Nano)
	return json.Marshal(out)
}

// writeRecords rewrites the whole index atomically, one JSON object per
// line, 0600.
func writeRecords(path string, records []record) error {
	var buf bytes.Buffer
	for _, rec := range records {
		b, err := encodeLine(rec)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return atomicfile.Write(path, buf.Bytes(), 0o600)
}
