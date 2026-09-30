package modeltable

import (
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// The keys a providers.toml stores inline, read on their own (plan 031 §3.8).
//
// A running native session keeps the model table it opened with (P8): a key
// stored after that — by a hand edit, or by another craze — never becomes a
// credential it sends. It can still turn up in what the session's tools show
// (`cat ~/.craze/native/providers.toml`, an echo of a pasted key), so the
// session learns it, for redaction only, at each turn's start. That reading is
// this file's: the one file, strictly decoded as Load decodes it, with no
// catalog, no models.toml and no merge — a key is a key whichever entry holds
// it, and a models.toml the owner is half-way through editing must not stop a
// session from learning what it has to redact.

// StoredKey is one provider's inline api_key as providers.toml holds it:
// untrimmed and unjudged. Key is a Secret, so a printed StoredKey shows
// which provider has a key and never the key.
type StoredKey struct {
	Provider string
	Key      Secret
}

// StoredKeys reads the inline keys of dir's providers.toml, one per provider
// that writes a non-blank api_key, in provider-id order. A missing file is no
// keys and no error.
//
// The file is decoded exactly as Load decodes it (decodeStrict): TOML syntax,
// a key the schema lacks and a version other than Version each fail the
// reading, with an error that names the file and the line, table or key and
// never a value. Nothing else is judged — not the entries' drivers or
// endpoints, and not the keys themselves: what may be learned is the caller's
// call (the harness's LearnKeys holds each value to KeyProblem), and one bad
// value must not hide the others. The file's mode is left alone: this is a
// reading, not a load (tighten is Load's).
func StoredKeys(dir string) ([]StoredKey, error) {
	if dir == "" {
		return nil, errors.New("modeltable: no directory to read keys from")
	}
	path := filepath.Join(dir, ProvidersFile)
	data, has, err := readIfPresent(path)
	if err != nil || !has {
		return nil, err
	}
	var doc providersOverlay
	if err := decodeStrict(path, data, &doc); err != nil {
		return nil, err
	}
	var out []StoredKey
	for _, id := range slices.Sorted(maps.Keys(doc.Providers)) {
		if k := doc.Providers[id].APIKey; k != nil && strings.TrimSpace(*k) != "" {
			out = append(out, StoredKey{Provider: id, Key: Secret(*k)})
		}
	}
	return out, nil
}

// KeyProblem says why k, with surrounding whitespace trimmed, cannot be a key
// craze redacts: ErrKeyTooShort or ErrKeyOverlapsMarker, never quoting k. It is
// nil for a usable key, and for a blank one (no key at all). It is the rule an
// inline api_key is loaded by and an environment value is skipped by (plan 031
// §3.2), for a caller outside this package that is handed a key to judge — a
// running session learning one stored after it opened (plan 031 §3.8, r2-3).
func KeyProblem(k string) error { return keyProblem(strings.TrimSpace(k)) }
