package modeltable

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/charliek/craze/internal/atomicfile"
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
	doc, err := readKeyFile(filepath.Join(dir, ProvidersFile))
	if err != nil {
		return nil, err
	}
	var out []StoredKey
	for _, id := range slices.Sorted(maps.Keys(doc.Providers)) {
		if k, ok := doc.Providers[id].storedKey(); ok {
			out = append(out, StoredKey{Provider: id, Key: Secret(k)})
		}
	}
	return out, nil
}

// storedKey is o's inline api_key as written, untrimmed, and whether it is
// one: a non-blank value, usable or not.
func (o providerOverlay) storedKey() (string, bool) {
	if o.APIKey == nil || strings.TrimSpace(*o.APIKey) == "" {
		return "", false
	}
	return *o.APIKey, true
}

// KeyProblem says why k, with surrounding whitespace trimmed, cannot be a key
// craze redacts: ErrKeyTooShort or ErrKeyOverlapsMarker, never quoting k. It is
// nil for a usable key, and for a blank one (no key at all). It is the rule an
// inline api_key is loaded by and an environment value is skipped by (plan 031
// §3.2), for a caller outside this package that is handed a key to judge — a
// running session learning one stored after it opened (plan 031 §3.8, r2-3).
func KeyProblem(k string) error { return keyProblem(strings.TrimSpace(k)) }

// The key store (plan 031 §3.7, P1): `craze auth` keeps a provider's key where
// a hand-written one has always lived, as the inline api_key of its
// providers.toml entry, which Resolve tries after the provider's env_keys. No
// other file, no keychain. Three operations, each on the one file:
//
//   - Providers lists every provider a key can be given to — the catalog's
//     and the user file's own — and how each is funded now. It is structural:
//     a stored key that cannot be one is reported, not a failure, so a bad
//     key can still be listed, replaced and removed (panel astra 14).
//   - SetKey stores a provider's key; RemoveKey clears it. Each reads the file
//     strictly and without judging its keys, changes one entry, and writes
//     the file back whole, atomically, at 0600, under an exclusive lock
//     (providers.toml.lock) that must be taken — a failure to take it is the
//     caller's error, never skipped (astra 18). Only the credential the call
//     changes is judged (r2-7): every other entry, its key included, is
//     written back exactly as it was read, so two broken keys can be repaired
//     one at a time. models.toml is never written.
//
// The rewrite keeps every key an entry had — source, and an explicit empty
// env_keys = [], included (an absent env_keys would inherit the shipped
// variables again) — but not comments or layout, which the header says. A
// providers.toml that is a symlink is refused rather than replaced by a
// regular file: its target is the owner's to edit.
//
// A directory whose models.toml says `catalog = false` (P10) has no catalog
// for key management either: its providers are the user file's alone, so a
// key is never written for a shipped provider that directory's table does
// not have (an incomplete entry there fails every load). models.toml is read
// for that one key only, leniently — a models.toml that does not parse leaves
// the catalog on, since key management must keep working while it is broken
// (§3.7).

// keyFileHeader is written above the providers.toml SetKey and RemoveKey
// write. Every earlier comment is gone by then, so it says so, and names the
// two things that write it: craze auth and the TUI's /connect (plan 031 §3.7).
const keyFileHeader = `# craze native harness: API keys and provider overrides.
# craze rewrites this file for "craze auth" and /connect: comments are not kept. Keep it 0600; never paste it.

`

// keyLockName is the lock every writer of providers.toml takes, beside it.
const keyLockName = ProvidersFile + ".lock"

var (
	// ErrEmptyKey is SetKey's error for a key that is blank once trimmed:
	// storing "" would read as no key at all.
	ErrEmptyKey = errors.New("modeltable: the API key is empty")

	// ErrUnknownProvider is SetKey's and RemoveKey's error for a provider id
	// that neither the catalog nor the user's providers.toml has.
	ErrUnknownProvider = errors.New("modeltable: unknown provider")

	// ErrSymlinkedKeyFile is SetKey's and RemoveKey's error for a
	// providers.toml that is a symlink.
	ErrSymlinkedKeyFile = errors.New("modeltable: providers.toml is a symlink")

	// ErrSignInProvider is SetKey's refusal of a key for the ChatGPT plan's
	// provider, which is funded by signing in and never by a key (plan 033
	// §3.11): a stored one would fund nothing, and validateProvider refuses
	// it on that driver.
	ErrSignInProvider = errors.New(`modeltable: the ChatGPT plan is funded by signing in, not by an API key: run "craze auth login chatgpt"`)
)

// KeySource is how a provider is funded: the variable, or the stored key,
// that Resolve would take its key from.
type KeySource string

const (
	// KeyFromEnv is one of the provider's env_keys, set to a usable key.
	KeyFromEnv KeySource = "env"
	// KeyStored is a usable inline api_key in providers.toml, with no usable
	// variable ahead of it.
	KeyStored KeySource = "stored"
	// KeyNone is neither: the provider's models cannot be used. For the
	// ChatGPT plan's provider it is "not signed in".
	KeyNone KeySource = "none"
	// KeySignedIn is the ChatGPT plan's provider with its sign-in funding it:
	// the token file there and plan usage granted (plan 033 §3.11, P34).
	KeySignedIn KeySource = "signedin"
	// KeyPlanDisabled is the ChatGPT plan's provider signed in to by an
	// account that did not grant craze plan usage: not funded, and signing in
	// again with consent is how to fund it.
	KeyPlanDisabled KeySource = "plan_disabled"
)

// ProviderInfo is one provider as key management sees it (Providers).
type ProviderInfo struct {
	// ID is the provider's id, the key of its providers.toml table.
	ID string
	// Name is its display name: its entry's name, or the id when it has
	// none.
	Name string
	// EnvKeys are the variables it takes a key from, in order, as the merge
	// makes them (an entry that changes the endpoint drops the shipped ones,
	// plan 031 §3.2).
	EnvKeys []string
	// Via is how it is funded now, judged exactly as Resolve judges it: the
	// first of EnvKeys set to a usable key, else a usable stored key — or,
	// for the ChatGPT plan's provider (SignIn), its sign-in: KeySignedIn,
	// KeyPlanDisabled or KeyNone. Connected says whether that funds it.
	Via KeySource
	// SignIn says the provider is funded by signing in, never by a key: the
	// ChatGPT plan's (plan 033 §3.11). SetKey refuses a key for it.
	SignIn bool
	// EnvVar is the variable that funds it when Via is KeyFromEnv, "" else.
	EnvVar string
	// Stored says providers.toml holds a non-blank api_key for it, usable or
	// not.
	Stored bool
	// StoredProblem is why that stored key cannot be one — ErrKeyTooShort or
	// ErrKeyOverlapsMarker, which name the rule and never the value — and nil
	// for a usable key or none. A table with such a key does not load, so a
	// caller shows it to have it replaced or removed.
	StoredProblem error
}

// Connected reports whether p is funded now, so its models can be used: a
// variable, a stored key, or a sign-in with plan usage. A sign-in whose plan
// usage is off is not.
func (p ProviderInfo) Connected() bool {
	switch p.Via {
	case KeyFromEnv, KeyStored, KeySignedIn:
		return true
	}
	return false
}

// Providers is every provider a key can be given to in dir: the shipped
// catalog's (unless dir's models.toml says `catalog = false`) and every entry
// of dir's providers.toml, each with how it is funded now through getenv (nil
// is os.Getenv), sorted by display name and then id. Only providers.toml is
// read strictly: its TOML syntax, a key the schema lacks or a wrong version is
// an error, naming the file and never a value. Nothing else fails it — not an
// unusable stored key (ProviderInfo.StoredProblem), and not an entry the merge
// would drop, which is listed from what it writes — so a broken key can be
// replaced or removed (plan 031 §3.7, astra 14). It writes nothing.
func Providers(dir string, getenv func(string) string) ([]ProviderInfo, error) {
	cat, err := shippedCatalog()
	if err != nil {
		return nil, fmt.Errorf("modeltable: the shipped catalog: %w", err)
	}
	return providersWith(dir, getenv, cat)
}

// providersWith is Providers over cat, for tests; it never changes cat.
func providersWith(dir string, getenv func(string) string, cat *Catalog) ([]ProviderInfo, error) {
	if dir == "" {
		return nil, errors.New("modeltable: no directory to read keys from")
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	path := filepath.Join(dir, ProvidersFile)
	doc, err := readKeyFile(path)
	if err != nil {
		return nil, err
	}
	if catalogOff(dir) {
		cat = nil
	}
	// The merge keeps every catalog provider, so it and the file's own
	// entries are every provider there is.
	merged := keylessProviders(cat, path, doc)
	for id, o := range doc.Providers {
		if _, ok := merged[id]; !ok { // an entry of the user's the merge drops is still theirs to key
			merged[id] = o.complete()
		}
	}

	out := make([]ProviderInfo, 0, len(merged))
	for id, p := range merged {
		info := ProviderInfo{ID: id, Name: cmp.Or(p.Name, id), EnvKeys: slices.Clone(p.EnvKeys), Via: KeyNone}
		if p.Driver == DriverChatGPT {
			// Funded by the sign-in alone, as Resolve judges it (plan 033
			// §3.11): a key stored for it by hand is listed, so it can be
			// removed, and funds nothing.
			info.SignIn, info.Via = true, signInVia(dir)
			if k, ok := doc.Providers[id].storedKey(); ok {
				info.Stored, info.StoredProblem = true, KeyProblem(k)
			}
			out = append(out, info)
			continue
		}
		if name, _, _ := firstEnvKey(getenv, p.EnvKeys); name != "" {
			info.Via, info.EnvVar = KeyFromEnv, name
		}
		if k, ok := doc.Providers[id].storedKey(); ok {
			info.Stored = true
			info.StoredProblem = KeyProblem(k)
			if info.Via == KeyNone && info.StoredProblem == nil {
				info.Via = KeyStored
			}
		}
		out = append(out, info)
	}
	// Ids are unique, so the order is total whatever the map's was.
	slices.SortFunc(out, func(a, b ProviderInfo) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// keylessProviders is doc's providers merged over cat exactly as Load merges
// them — the legacy rule, the dependent fields, a half-written endpoint
// dropped — but with every inline key left out, so that no key, valid or not,
// takes part: what Providers reads of the merge is names and variables. An
// entry the merge drops is missing from the result. A nil or empty cat is no
// catalog: each entry stands alone, as LoadWith reads it then.
func keylessProviders(cat *Catalog, path string, doc providersOverlay) map[string]Provider {
	keyless := providersOverlay{Version: doc.Version, Providers: make(map[string]providerOverlay, len(doc.Providers))}
	for id, o := range doc.Providers {
		o.APIKey = nil
		keyless.Providers[id] = o
	}
	if cat.empty() {
		out := make(map[string]Provider, len(keyless.Providers))
		for id, o := range keyless.Providers {
			out[id] = o.complete()
		}
		return out
	}
	return merge(cat.Clone(), path, filepath.Join(filepath.Dir(path), ModelsFile), &keyless, &modelsOverlay{}).Providers
}

// SetKey stores key as providerID's inline api_key in dir's providers.toml
// (plan 031 §3.7), trimmed: the provider must be one the catalog ships or one
// the file already has. An empty key, or one KeyProblem refuses, is refused
// before anything is read or written, with an error that names the rule and
// never the value (ErrEmptyKey, ErrKeyTooShort, ErrKeyOverlapsMarker through
// errors.Is), and so is a key for the ChatGPT plan's provider, which signs in
// (ErrSignInProvider, plan 033 §3.11), once the file is read under the lock
// and nothing written. The rest is the store's rule: see the comment above
// keyFileHeader.
func SetKey(dir, providerID, key string) error {
	cat, err := shippedCatalog()
	if err != nil {
		return fmt.Errorf("modeltable: the shipped catalog: %w", err)
	}
	return setKeyWith(dir, providerID, key, cat)
}

func setKeyWith(dir, id, key string, cat *Catalog) error {
	k := strings.TrimSpace(key)
	rule := keyProblem(k)
	if k == "" {
		rule = ErrEmptyKey
	}
	if rule != nil {
		return &keyRefusedError{provider: id, rule: rule}
	}
	return editKeyFile(dir, id, cat, true, func(doc *providersOverlay) (bool, error) {
		if signInDriver(dir, id, cat, *doc) {
			return false, ErrSignInProvider
		}
		o := doc.Providers[id]
		o.APIKey = &k
		doc.Providers[id] = o
		return true, nil
	})
}

// signInDriver reports whether id's entry, as dir's providers.toml writes it
// over the catalog, is on the ChatGPT plan's driver: its own driver when it
// writes one, else the catalog's (unless dir's models.toml turns the catalog
// off).
func signInDriver(dir, id string, cat *Catalog, doc providersOverlay) bool {
	if d := doc.Providers[id].Driver; d != nil {
		return *d == DriverChatGPT
	}
	if catalogOff(dir) || cat.empty() {
		return false
	}
	return cat.Providers[id].Driver == DriverChatGPT
}

// RemoveKey clears providerID's inline api_key in dir's providers.toml, and
// reports whether it held a non-blank one. Only the key goes: an entry that
// sets anything else (a name, an endpoint, variables) keeps it, and one left
// with nothing but its source is dropped, since that alone changes nothing
// (panel GLM 5). A provider with no key stored is (false, nil) with nothing
// written, as is one of a directory with no providers.toml, which is not
// created. The provider must be one the catalog ships or one the file has.
func RemoveKey(dir, providerID string) (removed bool, err error) {
	cat, err := shippedCatalog()
	if err != nil {
		return false, fmt.Errorf("modeltable: the shipped catalog: %w", err)
	}
	return removeKeyWith(dir, providerID, cat)
}

func removeKeyWith(dir, id string, cat *Catalog) (removed bool, err error) {
	if dir != "" {
		if _, err := os.Lstat(filepath.Join(dir, ProvidersFile)); errors.Is(err, fs.ErrNotExist) {
			// Nothing stored anywhere: no directory or lock is made to say so.
			return false, knownProvider(dir, id, cat, providersOverlay{})
		}
	}
	err = editKeyFile(dir, id, cat, false, func(doc *providersOverlay) (bool, error) {
		o := doc.Providers[id]
		if o.APIKey == nil {
			return false, nil
		}
		_, removed = o.storedKey()
		o.APIKey = nil
		// Every field is a pointer, so this is "nothing but its source is
		// left" whatever fields the schema grows.
		if o == (providerOverlay{Source: o.Source}) {
			delete(doc.Providers, id)
		} else {
			doc.Providers[id] = o
		}
		return true, nil
	})
	return removed && err == nil, err
}

// editKeyFile is one read-modify-write of dir's providers.toml: the
// directory made (0700) when create says so, the lock taken, a symlink
// refused, the file read strictly without judging keys (a missing one is
// empty), id checked against the catalog and the file, and — when edit, which
// changes doc in place, says there is something to write — the whole file
// written back atomically at 0600. An error from edit is returned with
// nothing written.
func editKeyFile(dir, id string, cat *Catalog, create bool, edit func(doc *providersOverlay) (bool, error)) error {
	if dir == "" {
		// paths.NativeDir is "" with no home directory; joining "" would
		// write providers.toml into the working directory.
		return errors.New("modeltable: no directory to keep keys in")
	}
	if create {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("modeltable: %w", err)
		}
	}
	unlock, err := atomicfile.Lock(filepath.Join(dir, keyLockName))
	if err != nil {
		return fmt.Errorf("modeltable: locking %s: %w", filepath.Join(dir, ProvidersFile), err)
	}
	defer unlock()

	path := filepath.Join(dir, ProvidersFile)
	switch info, err := os.Lstat(path); {
	case err == nil && info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%w: %s points elsewhere, and craze does not write through it — edit its target by hand",
			ErrSymlinkedKeyFile, path)
	case err == nil && !info.Mode().IsRegular():
		return fmt.Errorf("modeltable: %s is not a regular file", path)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("modeltable: %w", err)
	}
	doc, err := readKeyFile(path)
	if err != nil {
		return err
	}
	if err := knownProvider(dir, id, cat, doc); err != nil {
		return err
	}
	if changed, err := edit(&doc); err != nil || !changed {
		return err
	}
	b, err := encodeKeyFile(doc)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(path, b, providersPerm); err != nil {
		return fmt.Errorf("modeltable: %w", err)
	}
	return nil
}

// knownProvider is nil when id is a provider a key can be stored for in dir:
// one doc has, or one the catalog ships when dir's models.toml leaves the
// catalog on.
func knownProvider(dir, id string, cat *Catalog, doc providersOverlay) error {
	if _, ok := doc.Providers[id]; ok {
		return nil
	}
	if !catalogOff(dir) && !cat.empty() {
		if _, ok := cat.Providers[id]; ok {
			return nil
		}
	}
	return fmt.Errorf("%w %q: neither craze's catalog nor %s has it", ErrUnknownProvider, id, filepath.Join(dir, ProvidersFile))
}

// readKeyFile decodes path strictly, as Load does (decodeStrict), and judges
// nothing else: not the entries' endpoints and not their keys. A missing file
// is an empty one, at the current version, with a non-nil map.
func readKeyFile(path string) (providersOverlay, error) {
	doc := providersOverlay{Version: Version}
	data, has, err := readIfPresent(path)
	if err != nil || !has {
		doc.Providers = map[string]providerOverlay{}
		return doc, err
	}
	if err := decodeStrict(path, data, &doc); err != nil {
		return providersOverlay{}, err
	}
	if doc.Providers == nil {
		doc.Providers = map[string]providerOverlay{}
	}
	return doc, nil
}

// encodeKeyFile renders doc as providers.toml: keyFileHeader, the version,
// and one table per provider in id order holding exactly the keys its entry
// wrote. The overlay's fields are pointers, so a key the entry left out stays
// out and one it wrote — empty or not, a key the rules refuse included — is
// written back as it was read.
func encodeKeyFile(doc providersOverlay) ([]byte, error) {
	entries := make(map[string]providerOverlay, len(doc.Providers))
	for id, o := range doc.Providers {
		if o.EnvKeys != nil && *o.EnvKeys == nil {
			o.EnvKeys = &[]string{} // the encoder skips a nil list: keep `env_keys = []`
		}
		entries[id] = o
	}
	return encodeFile(keyFileHeader, &providersDoc{Version: Version}, "providers", entries)
}

// catalogOff reports whether dir's models.toml turns the catalog off
// (`catalog = false`, P10). Only that key is read, leniently: a models.toml
// that is missing, unreadable or not valid TOML leaves the catalog on, so key
// management keeps working while the owner fixes it (plan 031 §3.7).
func catalogOff(dir string) bool {
	data, has, err := readIfPresent(filepath.Join(dir, ModelsFile))
	if err != nil || !has {
		return false
	}
	var d struct {
		Catalog *bool `toml:"catalog"`
	}
	if _, err := toml.Decode(string(data), &d); err != nil {
		return false
	}
	return d.Catalog != nil && !*d.Catalog
}

// keyRefusedError is SetKey's error for a key it will not store: the
// provider and the rule, never the value. It unwraps to the rule.
type keyRefusedError struct {
	provider string
	rule     error
}

func (e *keyRefusedError) Error() string {
	why := "it is empty"
	if !errors.Is(e.rule, ErrEmptyKey) {
		why = keyReason(e.rule)
	}
	return fmt.Sprintf("modeltable: the key for provider %q was not saved: %s", e.provider, why)
}

func (e *keyRefusedError) Unwrap() error { return e.rule }
